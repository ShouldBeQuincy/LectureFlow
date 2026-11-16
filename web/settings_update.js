// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// The in-app update in the settings page's "App" section (SPEC-ext
// "4. 应用内更新（不依赖自有服务器）"; the Go side is internal/selfupdate,
// its README and internal/api/handlers_selfupdate.go):
//
//   应用更新     the status: not configured (why) / not checked yet / checking /
//                up to date / a newer version x.y.z with its notes / downloading
//                (progress) / verifying imported files / pending, installed at
//                the next start / the last check or download failed (a release
//                whose update.json or signature is not there yet has its own
//                line: no_manifest, not "check failed"); a failed check next
//                to a version that can still be installed is a note under it,
//                with the time of the next automatic check; and the
//                outcome of the last installation (result.json: installed,
//                rolled back to the previous version, failed) with its reason
//                under "查看详情"; a version whose installation failed is
//                not offered again until a newer one appears (install_failed:
//                "安装 x.y.z 没有成功" and "重新下载" to try it again).
//                Buttons: check now, download, cancel the download, discard
//                the pending update. A privacy note.
//   从文件更新   update.json, update.json.minisig and the package, or one zip
//                holding them: the system dialog or a drop (desktop app: the
//                local paths are sent, nothing is uploaded)
//   自动检查更新 at most once a day (selfupdate.auto_check)
//   更新源       extra sources (mirrors, proxies), at most MAX_SOURCES; every
//                source is verified against the same signature
//
// Web mode has no in-app update (reason web_mode: nothing could install
// it): one row says "desktop app only". A server without the updater
// (older version, 503 server.unavailable) shows nothing. Everything shown is
// what the server returned: an action disables its button until the answer
// comes back, then the status of the answer (and of SSE changed
// {kind: "selfupdate"}) is shown; download progress is updated in place so
// the rest of the App section (an address being typed) is left alone.
//
// update_hint.js uses loadStatus, configured and recentFailure for the quiet
// line at the bottom of the workbench.

import { h, btn, iconBtn, toast, confirmDialog, dropzone } from './dom.js';
import { icon } from './icons.js';
import { t } from './i18n.js';
import * as U from './util.js';
import { MOCK, MOCK_SCENARIO, ApiError, errorText, localPath } from './api.js';
import { externalLinkFailed } from './onboarding_api.js';

// States of selfupdate.Status during which an operation runs.
const BUSY = new Set(['checking', 'downloading', 'importing']);
// The errors of a failed download (the others are those of a check).
const DOWNLOAD_ERRORS = new Set(['selfupdate.download_failed', 'selfupdate.checksum', 'selfupdate.io_failed']);
// internal/selfupdate maxSources
export const MAX_SOURCES = 8;
// The files of an offline update: update.json, its signature, the package
// (Windows installer or portable .exe, macOS .zip or .dmg), or one zip.
export const IMPORT_ACCEPT = '.json,.minisig,.exe,.zip,.dmg';
// A failed installation is mentioned on the workbench for a week.
const FAILURE_DAYS = 7;
const BUSY_MS = 1500;
const IDLE_MS = 10 * 60 * 1000;

function withMock(path) {
  if (!MOCK) return path;
  return path + (path.includes('?') ? '&' : '?') + 'mock=' + encodeURIComponent(MOCK_SCENARIO);
}

/**
 * suRequest(method, path, body?) → the JSON answer. body is a FormData
 * (multipart) or a value sent as JSON. Errors are ApiErrors with the
 * translated text (api.js errorText) and code, params and detail.
 */
export async function suRequest(method, path, body) {
  const init = { method, cache: 'no-store', headers: {} };
  if (body instanceof FormData) init.body = body;
  else if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(withMock(path), init);
  } catch {
    throw new ApiError(0, t('common.net.unreachable'));
  }
  const text = await res.text().catch(() => '');
  if (!res.ok) {
    let b = null;
    try {
      b = JSON.parse(text);
    } catch {
      b = null;
    }
    throw new ApiError(res.status, errorText(res.status, b), b);
  }
  if (res.status === 204 || !text) return null;
  try {
    return JSON.parse(text);
  } catch {
    throw new ApiError(res.status, t('common.net.bad_response'));
  }
}

/**
 * loadStatus() → GET /api/selfupdate, or null when this server has no
 * in-app update (an older server: 404; no updater: 503 server.unavailable).
 * A network failure throws (the caller keeps what it shows).
 */
export async function loadStatus() {
  try {
    return await suRequest('GET', '/api/selfupdate');
  } catch (e) {
    if (e.status === 404 || e.status === 405 || e.status === 501 || e.status === 503) return null;
    throw e;
  }
}

/** True when the in-app update works here (it decides what the workbench says). */
export const configured = (st) => !!st && typeof st.state === 'string' && st.state !== 'not_configured';

/** The last installation failed or was rolled back within FAILURE_DAYS (result.json), else null. */
export function recentFailure(st, now = Date.now()) {
  const r = st && st.last_result;
  if (!r || (r.outcome !== 'rolled_back' && r.outcome !== 'failed')) return null;
  const at = Date.parse(r.at);
  if (!Number.isFinite(at) || now - at > FAILURE_DAYS * 86400000) return null;
  return r;
}

/** The sentence for result.json's outcome (installed / rolled_back / failed). */
export function resultText(r) {
  if (!r) return '';
  if (r.outcome === 'installed') return t('settings.update.result.installed', { version: r.version, when: U.relTime(r.at) });
  if (r.outcome === 'rolled_back') return t('settings.update.result.rolled_back', { version: r.version, from: r.from });
  if (r.outcome === 'failed') return t('settings.update.result.failed', { version: r.version });
  return '';
}

/** True when Status.error is that of a download (else of a check). */
export const downloadError = (st) => !!(st && st.error && DOWNLOAD_ERRORS.has(st.error.code));

// The text of a selfupdate error: "no source" says what to do here; a
// signature whose trusted comment names no version (minisign's default
// comment) is not "the version  differs".
function errorInfoText(info) {
  const p = (info && info.params) || {};
  if (info && info.code === 'selfupdate.not_configured' && p.reason === 'no_source') return t('settings.update.no_source');
  if (info && info.code === 'selfupdate.version_mismatch' && !p.signed) return t('settings.update.version_unbound', { manifest: p.manifest || '' });
  return U.errorInfoText(info, t('common.unknown_error'));
}

/**
 * R75 M4: an offline update refused for its size or SHA-256 (the files the
 * user chose do not belong together, or one is incomplete or too large;
 * internal/selfupdate importer.go) has nothing to download again: its own
 * words, {text, detail}. null for any other error (the shared text).
 */
export function importErrorInfo(e) {
  if (!e || e.code !== 'selfupdate.checksum') return null;
  return { text: t('settings.update.file_checksum', { file: (e.params && e.params.file) || '' }), detail: e.detail || '' };
}

/** True when Status.error is that of a check (a note next to an update that can be installed). */
export const checkError = (st) => !!(st && st.error && !downloadError(st));

// The time of the next automatic check: the time alone today, else the day too.
function nextCheckText(iso) {
  const d = new Date(iso);
  if (!iso || isNaN(d.getTime())) return '';
  const now = new Date();
  const today = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return today ? U.fmtHM(d) : U.fmtDayShort(d, true);
}

function notConfiguredText(reason) {
  switch (reason) {
    case 'no_public_key':
      return t('settings.update.not_configured.no_public_key');
    case 'dev_build':
      return t('settings.update.not_configured.dev_build');
    case 'unsupported_platform':
      return t('settings.update.not_configured.unsupported_platform');
    case 'no_source':
      return t('settings.update.not_configured.no_source');
    default:
      return t('errors.selfupdate.not_configured');
  }
}

const progressOf = (p) => (p && p.bytes_total > 0 ? Math.max(0, Math.min(1, p.bytes_done / p.bytes_total)) : 0);
const httpsURL = (u) => (typeof u === 'string' && /^https:\/\/\S+$/i.test(u) ? u : '');

// An https link opened by the desktop shell (the browser in web mode).
function extLink(url, text, fkey) {
  return h(
    'a',
    {
      class: 'set-link',
      href: url,
      target: '_blank',
      rel: 'noopener noreferrer',
      dataset: { fkey },
      onclick: (e) => {
        const lf = window.lectureflow;
        if (lf && typeof lf.openExternal === 'function') {
          e.preventDefault();
          Promise.resolve(lf.openExternal(url)).catch(externalLinkFailed);
        }
      },
    },
    text,
  );
}

// "查看详情": the server's own words (English) folded away.
function details(text, fkey) {
  if (!text) return null;
  return h('details', { class: 'dl-detail-more' }, h('summary', { dataset: { fkey } }, t('settings.update.details')), h('pre', { class: 'dl-detail-text mono', lang: 'en' }, text));
}

const note = (ic, text, cls = '', extra = null) =>
  h('div', { class: 'set-note' + (cls ? ' ' + cls : '') }, icon(ic, { size: 13 }), h('div', { class: 'su-note-body' }, h('span', null, text), extra));

/**
 * createUpdateSection({formRow, switchBtn, rerender}): the helpers of
 * view_settings.js (through settings_app.js); rerender() redraws the App
 * section. → {rows(), load(), onChanged(ev), onSettingsChanged(), destroy()}.
 */
export function createUpdateSection(ui) {
  let alive = true;
  let st = null; // GET /api/selfupdate
  let missing = false; // the server has no in-app update: no rows
  let loadError = '';
  let settings = null; // GET /api/selfupdate/settings
  let acting = ''; // check | download | cancel | discard | import | auto | sources
  let actError = null; // {text, detail}: the last failed action
  let importError = null; // {text, detail}: the last failed offline update
  let sourceError = '';
  let loading = false;
  let again = false;
  let timer = null;
  let shownKey = '';
  const live = { text: null, size: null, meter: null, fill: null }; // the download, updated in place
  const redraw = () => alive && ui.rerender();

  // The address being typed survives the section's re-renders.
  const srcInput = h('input', {
    type: 'url',
    class: 'input cu-src-url',
    placeholder: t('settings.update.source_placeholder'),
    'aria-label': t('settings.update.source_input'),
    spellcheck: 'false',
    autocomplete: 'off',
    dataset: { fkey: 'set:upd:src' },
    onkeydown: (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        addSource();
      }
    },
    oninput: () => {
      if (sourceError) {
        sourceError = '';
        redraw();
      }
    },
  });

  // ---- data ----

  async function load() {
    if (loading) {
      again = true;
      return;
    }
    loading = true;
    clearTimeout(timer);
    try {
      const s = await loadStatus();
      if (!alive) return;
      if (s === null) {
        missing = true;
        st = null;
        redraw();
        return;
      }
      missing = false;
      loadError = '';
      let set = settings;
      if (s.state !== 'not_configured') {
        try {
          set = await suRequest('GET', '/api/selfupdate/settings');
        } catch {
          // keep the shown settings
        }
      }
      if (!alive) return;
      apply(s, set);
    } catch (e) {
      if (!alive) return;
      if (!st) {
        loadError = e.message || t('common.load_failed');
        redraw();
      }
    } finally {
      loading = false;
      if (alive) {
        if (again) {
          again = false;
          load();
        } else {
          timer = setTimeout(load, st && BUSY.has(st.state) ? BUSY_MS : IDLE_MS);
        }
      }
    }
  }

  // What the rows show apart from the download's progress.
  const shapeOf = (s, set) => JSON.stringify([s ? { ...s, progress: !!s.progress } : null, set]);

  function apply(s, set) {
    st = s;
    settings = set || settings;
    if (shapeOf(st, settings) === shownKey && live.text && live.text.isConnected) {
      updateLive();
      return;
    }
    redraw();
  }

  function updateLive() {
    const a = st.available;
    const p = st.progress;
    const frac = progressOf(p);
    live.text.textContent = t('settings.update.state.downloading', { version: a ? a.version : '', percent: U.fmtPercent(frac, 0) });
    if (live.size) live.size.textContent = p ? t('settings.update.progress', { done: U.fmtBytes(p.bytes_done), total: U.fmtBytes(p.bytes_total) }) : '';
    live.fill.style.width = `${frac * 100}%`;
    live.meter.setAttribute('aria-valuenow', String(Math.round(frac * 100)));
  }

  // ---- actions ----

  const errInfo = (e) => ({
    text: e.code === 'selfupdate.version_mismatch' ? errorInfoText({ code: e.code, params: e.params }) : e.message || t('common.unknown_error'),
    detail: e.detail || '',
  });

  async function act(name, fn) {
    if (acting) return;
    acting = name;
    actError = null;
    redraw();
    try {
      const s = await fn();
      if (alive && s && s.state) st = s;
    } catch (e) {
      if (alive) actError = errInfo(e);
    }
    acting = '';
    redraw();
    load();
  }

  const check = () => act('check', () => suRequest('POST', '/api/selfupdate/check'));
  const download = () => act('download', () => suRequest('POST', '/api/selfupdate/download'));
  // "Try again" for the version whose installation failed: downloading it
  // is the retry (the server forgets the failure then). Without that
  // version at hand (no check since), a check first; one that finds a
  // newer version just shows it.
  const retryFailed = () =>
    act('download', async () => {
      const f = st && st.install_failed;
      let s = st;
      const same = (x) => !!(f && x && x.available && x.available.version === f.version);
      if (!same(s)) s = await suRequest('POST', '/api/selfupdate/check');
      if (same(s) && s.state === 'install_failed' && s.available.direct && s.available.file) return suRequest('POST', '/api/selfupdate/download');
      return s;
    });
  const cancelDownload = () => act('cancel', () => suRequest('DELETE', '/api/selfupdate/download'));

  async function discard() {
    const p = st && st.pending;
    if (!p || acting) return;
    const ok = await confirmDialog({
      title: t('settings.update.discard_title', { version: p.version }),
      message: t('settings.update.discard_message', { version: p.version }),
      okText: t('settings.update.discard_ok'),
      danger: true,
    });
    if (!ok || !alive) return;
    act('discard', () => suRequest('DELETE', '/api/selfupdate/pending'));
  }

  // Offline update: the local paths in the desktop app, else the files.
  async function importFiles(files) {
    if (acting || !files || !files.length) return;
    acting = 'import';
    importError = null;
    actError = null;
    redraw();
    try {
      const paths = files.map((f) => localPath(f));
      let body;
      if (paths.every(Boolean)) body = { paths };
      else {
        body = new FormData();
        for (const f of files) if (f instanceof Blob) body.append('file', f, f.name);
      }
      const s = await suRequest('POST', '/api/selfupdate/import', body);
      if (alive && s && s.state) {
        st = s;
        if (s.pending) toast(t('settings.update.file_done', { version: s.pending.version }), 'success');
      }
    } catch (e) {
      if (alive) importError = importErrorInfo(e) || errInfo(e);
    }
    acting = '';
    redraw();
    load();
  }

  async function saveSettings(name, patch, onSaved) {
    if (acting) return;
    acting = name;
    sourceError = '';
    actError = null;
    redraw();
    try {
      const s = await suRequest('PATCH', '/api/selfupdate/settings', patch);
      if (alive && s) {
        settings = s;
        if (onSaved) onSaved();
      }
    } catch (e) {
      if (alive) {
        if (name === 'sources') sourceError = e.message || t('common.unknown_error');
        else actError = errInfo(e);
      }
    }
    acting = '';
    redraw();
  }

  const toggleAuto = () => settings && saveSettings('auto', { auto_check: !settings.auto_check });

  function addSource() {
    const v = srcInput.value.trim();
    if (!v || !settings || acting || settings.sources.length >= MAX_SOURCES) return;
    saveSettings('sources', { sources: [...settings.sources, v] }, () => {
      srcInput.value = '';
    });
  }

  const removeSource = (i) => settings && saveSettings('sources', { sources: settings.sources.filter((_, j) => j !== i) });

  // ---- render ----

  const busy = () => !!acting || (st && BUSY.has(st.state));
  const sub = (text, cls = '') => h('p', { class: 'upd-sub' + (cls ? ' ' + cls : '') }, text);

  function line(ic, text, tools, spin = false) {
    return h(
      'div',
      { class: 'upd-line' },
      icon(ic, { size: 14, class: spin ? 'spin' : '' }),
      h('span', { class: 'upd-text' }, text),
      tools && tools.filter(Boolean).length ? h('span', { class: 'upd-tools' }, tools) : null,
    );
  }

  const checkBtn = () => btn(t('settings.update.check'), { size: 'sm', icon: 'refresh-cw', disabled: busy(), onclick: check, fkey: 'set:upd:check' });

  // "发布于 … · 安装包 … · 更新说明" of the available version.
  function releaseLine(a) {
    const parts = [];
    if (a.released_at) parts.push(t('settings.update.released', { when: U.relTime(a.released_at) }));
    if (a.size > 0) parts.push(t('settings.update.size', { size: U.fmtBytes(a.size) }));
    const notes = httpsURL(a.notes_url);
    if (!parts.length && !notes) return null;
    return h('p', { class: 'upd-sub' }, parts.join(' · '), notes ? [parts.length ? ' · ' : '', extLink(notes, t('settings.update.notes'), 'set:upd:notes')] : null);
  }

  // The available version: the line, its release line and why it cannot
  // be downloaded here (too old, no package for this installation).
  function availableLines(a, failed) {
    const out = [];
    const canDownload = a.direct && !!a.file;
    const tools = [];
    if (canDownload && st.state !== 'downloading') {
      tools.push(
        btn(failed ? t('settings.update.retry_download') : t('settings.update.download'), {
          size: 'sm',
          icon: failed ? 'rotate-cw' : 'download',
          disabled: busy(),
          onclick: download,
          fkey: 'set:upd:download',
        }),
      );
    }
    tools.push(checkBtn());
    const text = failed ? t('settings.update.state.download_failed', { version: a.version }) : t('settings.update.state.available', { version: a.version });
    out.push(line(failed ? 'circle-alert' : 'download', [text, a.channel === 'beta' ? h('span', { class: 'chip chip-quiet su-chip' }, t('settings.app.channel.beta')) : null], tools));
    out.push(releaseLine(a));
    // r7 (T24 G8): "download it from the release page" comes with the link
    // to it (the version's notes page, as on the workbench: update_hint.js)
    const release = httpsURL(a.notes_url);
    const manual = (text) => sub([text, release ? [' ', extLink(release, t('update.open_release'), 'set:upd:release')] : null], 'is-warn');
    if (!a.direct) {
      out.push(manual(U.errorInfoText({ code: 'selfupdate.too_old', params: { current: st.current_version, min_version: a.min_version } })));
    } else if (!a.file) {
      out.push(manual(U.errorInfoText({ code: 'selfupdate.no_package', params: { platform: st.platform, kind: st.kind } })));
    }
    return out;
  }

  function pendingLines(p, main) {
    const out = [];
    const tools = [btn(t('settings.update.discard'), { size: 'sm', kind: 'ghost', icon: 'x', disabled: busy(), onclick: discard, fkey: 'set:upd:discard' })];
    const text = t('settings.update.state.pending', { version: p.version });
    if (main) {
      out.push(line('circle-check', text, [...tools, checkBtn()]));
    } else {
      out.push(h('div', { class: 'upd-line su-also' }, icon('circle-check', { size: 14 }), h('span', { class: 'upd-text' }, text), h('span', { class: 'upd-tools' }, tools)));
    }
    if (p.origin === 'file') out.push(sub(t('settings.update.origin_file')));
    const rb = st.rollback;
    if (rb && rb.version) out.push(sub(t('settings.update.rollback_to', { version: p.version, previous: rb.version })));
    return out;
  }

  function downloadingLines() {
    const a = st.available;
    const p = st.progress;
    const frac = progressOf(p);
    live.text = h('span', { class: 'upd-text' }, t('settings.update.state.downloading', { version: a ? a.version : '', percent: U.fmtPercent(frac, 0) }));
    live.fill = h('div', { class: 'meter-fill', style: { width: `${frac * 100}%` } });
    live.meter = h('div', { class: 'meter meter-accent upd-meter', role: 'progressbar', 'aria-label': t('update.progress_label'), 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(frac * 100)) }, live.fill);
    live.size = h('p', { class: 'upd-sub su-num' }, p ? t('settings.update.progress', { done: U.fmtBytes(p.bytes_done), total: U.fmtBytes(p.bytes_total) }) : '');
    return [
      h(
        'div',
        { class: 'upd-line' },
        icon('loader-circle', { size: 14, class: 'spin' }),
        live.text,
        h('span', { class: 'upd-tools' }, btn(t('settings.update.cancel_download'), { size: 'sm', kind: 'ghost', icon: 'x', disabled: !!acting, onclick: cancelDownload, fkey: 'set:upd:cancel' })),
      ),
      live.meter,
      live.size,
      a ? releaseLine(a) : null,
      sub(t('settings.update.download_note')),
    ];
  }

  // A failed check under an update that can still be installed (state
  // available or pending): the reason, folded details.
  function checkNote() {
    if (!checkError(st)) return null;
    return h('div', { class: 'su-err' }, sub(t('settings.update.last_check_failed', { error: errorInfoText(st.error) }), 'is-warn'), details(st.error.detail, 'set:upd:errdetail'));
  }

  // The version whose installation failed (install_failed): not offered
  // again until a newer one appears, its package removed; "Download again"
  // tries it once more (available is that version, or none until a check).
  function failedLines(f, a) {
    if (!f) return [];
    const same = !!a && a.version === f.version;
    const canRetry = !a || (same && a.direct && !!a.file);
    const tools = [];
    if (canRetry) tools.push(btn(t('settings.update.retry_download'), { size: 'sm', icon: 'rotate-cw', disabled: busy(), onclick: retryFailed, fkey: 'set:upd:retry' }));
    tools.push(checkBtn());
    return [
      line('circle-alert', t('settings.update.state.install_failed', { version: f.version }), tools),
      same ? releaseLine(a) : null,
      sub(t('settings.update.install_failed_note')),
      checkNote(),
    ];
  }

  function stateLines() {
    const a = st.available;
    switch (st.state) {
      case 'idle':
        return [line('clock', t('settings.update.state.idle', { version: st.current_version }), [checkBtn()])];
      case 'checking':
        return [line('loader-circle', t('settings.update.state.checking'), [checkBtn()], true)];
      case 'up_to_date':
        return [line('circle-check', t('settings.update.state.up_to_date', { version: st.current_version }), [checkBtn()])];
      case 'available':
        return [...(a ? availableLines(a, false) : []), checkNote(), st.pending ? pendingLines(st.pending, false) : null];
      case 'downloading':
        return downloadingLines();
      case 'importing':
        return [line('loader-circle', t('settings.update.state.importing'), null, true)];
      case 'pending':
        return st.pending ? [...pendingLines(st.pending, true), checkNote()] : [];
      case 'install_failed':
        return failedLines(st.install_failed, a);
      case 'error': {
        const dl = downloadError(st) && a;
        const err = st.error || {};
        // the latest release has no update.json or signature yet: not a
        // network problem, nothing to do but wait (or check again later)
        const waiting = !dl && err.code === 'selfupdate.no_manifest';
        let out;
        if (dl) out = availableLines(a, true);
        else if (waiting) out = [line('clock', t('settings.update.state.no_manifest'), [checkBtn()])];
        else out = [line('circle-alert', t('settings.update.state.check_failed'), [checkBtn()])];
        out.push(h('div', { class: 'su-err' }, sub(errorInfoText(err), waiting ? '' : 'is-warn'), details(err.detail, 'set:upd:errdetail')));
        if (!dl && a) out.push(...availableLines(a, false).slice(0, 2));
        if (st.pending) out.push(...pendingLines(st.pending, false));
        return out;
      }
      default:
        return [line('info', st.state, null)];
    }
  }

  function statusRow() {
    live.text = live.size = live.meter = live.fill = null;
    const r = st.last_result;
    // after a failed check: when the next automatic one comes (backing off)
    const quiet = !['checking', 'downloading', 'importing'].includes(st.state);
    const next = quiet && st.auto_check && checkError(st) ? nextCheckText(st.next_check) : '';
    const lastCheck =
      st.last_check && quiet
        ? sub([t('settings.update.last_check', { when: U.relTime(st.last_check) }), next ? ' · ' + t('settings.update.next_check', { when: next }) : ''], 'su-num')
        : null;
    let result = null;
    if (r && (r.outcome === 'rolled_back' || r.outcome === 'failed')) {
      result = note('triangle-alert', resultText(r), 'is-warn su-result', details(r.error, 'set:upd:resultdetail'));
    } else if (r && r.outcome === 'installed' && r.version === st.current_version) {
      result = note('circle-check', resultText(r), 'is-ok su-result');
    }
    const box = h(
      'div',
      { class: 'upd-hint su-status', role: 'status', dataset: { state: st.state } },
      stateLines(),
      lastCheck,
      actError ? h('div', { class: 'su-err', role: 'alert' }, sub(actError.text, 'is-warn'), details(actError.detail, 'set:upd:actdetail')) : null,
    );
    return ui.formRow(t('settings.update.label'), box, t('settings.update.hint'), [result, note('shield-check', t('settings.update.privacy'))]);
  }

  function importRow() {
    let ctl;
    if (acting === 'import' || st.state === 'importing') {
      ctl = note('loader-circle', t('settings.update.state.importing'), 'su-importing');
      ctl.querySelector('.icon').classList.add('spin');
    } else {
      ctl = dropzone({ text: t('settings.update.file_drop'), accept: IMPORT_ACCEPT, multiple: true, onFiles: importFiles, compact: true });
      ctl.style.width = '100%';
      ctl.style.margin = '0';
      const pick = ctl.querySelector('button');
      if (pick) pick.dataset.fkey = 'set:upd:file';
    }
    const err = importError ? h('div', { class: 'su-err', role: 'alert' }, note('circle-alert', importError.text, 'is-err'), details(importError.detail, 'set:upd:importdetail')) : null;
    return ui.formRow(t('settings.update.file_label'), ctl, t('settings.update.file_hint'), err);
  }

  function autoRow() {
    return ui.formRow(
      t('settings.update.auto_label'),
      ui.switchBtn(!!settings.auto_check, t('settings.update.auto_label'), toggleAuto, 'set:upd:auto'),
      t('settings.update.auto_hint'),
      acting === 'auto' ? note('loader-circle', t('settings.saving')) : null,
    );
  }

  function sourcesRow() {
    const list = settings.sources || [];
    const full = list.length >= MAX_SOURCES;
    const items = list.length
      ? h(
          'ol',
          { class: 'comp-list su-sources', style: { width: '100%' } },
          list.map((s, i) =>
            h(
              'li',
              { class: 'comp-row', style: { alignItems: 'center' } },
              h('span', { class: 'comp-row-icon' }, icon('server', { size: 14 })),
              h('code', { class: 'comp-row-main app-path-sm mono', title: s }, s),
              h('span', { class: 'comp-row-actions' }, iconBtn('trash-2', t('settings.update.source_remove', { url: s }), { size: 14, fkey: `set:upd:src:del:${i}`, onclick: () => removeSource(i) })),
            ),
          ),
        )
      : note('info', t('settings.update.sources_empty'));
    srcInput.disabled = full || acting === 'sources';
    if (sourceError) srcInput.setAttribute('aria-invalid', 'true');
    else srcInput.removeAttribute('aria-invalid');
    const add = h(
      'div',
      { class: 'set-inline su-add', style: { width: '100%' } },
      srcInput,
      btn(t('settings.update.source_add'), { size: 'sm', icon: 'plus', disabled: full || !!acting, onclick: addSource, fkey: 'set:upd:src:add' }),
    );
    return ui.formRow(t('settings.update.sources_label'), [items, add], t('settings.update.sources_hint'), [
      sourceError ? note('circle-alert', sourceError, 'is-err') : null,
      acting === 'sources' ? note('loader-circle', t('settings.saving')) : null,
      note('info', full ? t('settings.update.sources_max', { max: U.fmtInt(MAX_SOURCES) }) : t('settings.update.sources_formats', { token: '{url}' })),
    ]);
  }

  /** The rows of the App section (none before the first answer or without an updater). */
  function rows() {
    if (missing) return [];
    if (!st) {
      return loadError ? [ui.formRow(t('settings.update.label'), note('circle-alert', t('settings.update.load_error', { error: loadError }), 'is-warn'), null, btn(t('common.retry'), { size: 'sm', icon: 'rotate-cw', onclick: load, fkey: 'set:upd:reload' }))] : [];
    }
    if (st.state === 'not_configured') {
      live.text = null;
      shownKey = shapeOf(st, settings);
      if (st.reason === 'web_mode') {
        return [
          ui.formRow(
            t('settings.update.label'),
            h('div', { class: 'set-inline su-plain' }, h('span', { class: 'chip chip-quiet app-desktop-only' }, icon('lock', { size: 12 }), t('settings.app.desktop_only')), h('span', { class: 'dim small' }, t('settings.update.not_configured.web_mode'))),
            t('settings.update.hint'),
          ),
        ];
      }
      return [ui.formRow(t('settings.update.label'), note('info', notConfiguredText(st.reason), 'su-plain'), t('settings.update.hint'))];
    }
    const out = [statusRow(), importRow()];
    if (settings) out.push(autoRow(), sourcesRow());
    shownKey = shapeOf(st, settings);
    return out;
  }

  return {
    rows,
    load,
    // SSE changed {kind: "selfupdate"} (status, progress, settings)
    onChanged(ev) {
      if (ev && ev.kind === 'selfupdate') load();
    },
    // SSE changed {kind: "settings"}: the update channel decides what is offered
    onSettingsChanged() {
      load();
    },
    destroy() {
      alive = false;
      clearTimeout(timer);
    },
  };
}
